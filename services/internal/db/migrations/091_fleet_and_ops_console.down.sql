-- 091_fleet_and_ops_console.down.sql
-- Reverses 091: drops the fleet/promotion-gate and ops-console tables.
-- Children first (FK order).

BEGIN;

DROP TABLE IF EXISTS listing_proposals;
DROP TABLE IF EXISTS deploy_windows;
DROP TABLE IF EXISTS release_promotions;
DROP TABLE IF EXISTS releases;
DROP TABLE IF EXISTS server_actions;
DROP TABLE IF EXISTS fleet_hosts;
DROP TABLE IF EXISTS environments;

COMMIT;
