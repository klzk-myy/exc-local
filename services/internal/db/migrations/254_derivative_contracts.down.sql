-- 254_derivative_contracts.down.sql
BEGIN;

ALTER TABLE settlement_instructions
    DROP COLUMN IF EXISTS derivative_contract_id,
    DROP COLUMN IF EXISTS leg_tag;

DROP TABLE IF EXISTS ndf_fixings;
DROP TABLE IF EXISTS derivative_contracts;
DROP TYPE IF EXISTS derivative_leg_tag_enum;
DROP TYPE IF EXISTS derivative_contract_status_enum;
DROP TYPE IF EXISTS derivative_kind_enum;

COMMIT;
