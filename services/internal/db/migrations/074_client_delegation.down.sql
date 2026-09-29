-- 074_client_delegation.down.sql — drop Phase-12 Task 12.3.11 tables.

BEGIN;

DROP TABLE IF EXISTS client_delegation_events;
DROP TABLE IF EXISTS client_approval_decisions;
DROP TABLE IF EXISTS client_approval_requests;
DROP TABLE IF EXISTS client_approval_policies;
DROP TABLE IF EXISTS client_role_bindings;
DROP TABLE IF EXISTS client_delegated_users;

COMMIT;
