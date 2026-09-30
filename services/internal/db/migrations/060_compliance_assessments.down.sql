-- 060_compliance_assessments.down.sql — drop the Task 21.3.17
-- assessment engine tables in FK-safe order.

BEGIN;

DROP TABLE IF EXISTS fx_gc_statements;
DROP TABLE IF EXISTS compliance_assessments;
DROP TABLE IF EXISTS compliance_assessment_runs;

COMMIT;
