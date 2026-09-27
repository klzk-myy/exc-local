-- 006_create_trades.down.sql
BEGIN;

-- Remove partman registration (no table drops done by partman itself),
-- then drop the partitioned parent — CASCADE drops all child partitions.
-- pg_partman 5.x also leaves behind the per-parent template table
-- (template_public_trades); drop it for a clean rollback.
DELETE FROM public.part_config WHERE parent_table = 'public.trades';
DROP TABLE IF EXISTS trades CASCADE;
DROP TABLE IF EXISTS template_public_trades CASCADE;

COMMIT;
