-- 090_admin_rbac.down.sql — drop Phase-07 RBAC schema (reverse order).

BEGIN;

DROP TABLE IF EXISTS admin_break_glass_grants CASCADE;
DROP TABLE IF EXISTS admin_recert_decisions CASCADE;
DROP TABLE IF EXISTS admin_recert_campaigns CASCADE;
DROP TABLE IF EXISTS admin_dual_control_requests CASCADE;
DROP TRIGGER IF EXISTS trg_admin_role_bindings_system ON admin_role_bindings;
DROP FUNCTION IF EXISTS admin_role_binding_system_guard();
DROP TABLE IF EXISTS admin_role_bindings CASCADE;
DROP TABLE IF EXISTS principal_role_systems CASCADE;

COMMIT;
