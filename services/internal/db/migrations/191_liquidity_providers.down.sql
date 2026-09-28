-- 191_liquidity_providers.down.sql

BEGIN;

DROP TABLE IF EXISTS lp_performance_alerts;
DROP TABLE IF EXISTS lp_instrument_configs;
DROP TABLE IF EXISTS liquidity_providers;

COMMIT;
