-- 085_option_spread_offsets.down.sql
BEGIN;

DROP TABLE IF EXISTS option_spread_offset_params;
DROP TABLE IF EXISTS option_spread_offsets;
DROP TYPE IF EXISTS option_spread_param_status_enum;
DROP TYPE IF EXISTS option_spread_offset_status_enum;
DROP TYPE IF EXISTS option_spread_type_enum;

COMMIT;
