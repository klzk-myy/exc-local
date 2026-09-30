-- 248_surveillance_cases.down.sql — Phase-21 Task 21.3.21.

BEGIN;

DROP TRIGGER IF EXISTS case_evidence_immutable_trg ON surveillance_case_evidence;
DROP FUNCTION IF EXISTS p21_case_evidence_immutable;
DROP TABLE IF EXISTS surveillance_case_evidence;
DROP TABLE IF EXISTS surveillance_cases;

COMMIT;
