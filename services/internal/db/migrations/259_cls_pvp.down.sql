-- 259_cls_pvp — down

BEGIN;

DROP TABLE IF EXISTS cls_instruction_events;
DROP TABLE IF EXISTS cls_settlement_instructions;
DROP TYPE IF EXISTS cls_settlement_route_enum;
DROP TYPE IF EXISTS cls_instruction_status_enum;
DROP TABLE IF EXISTS cls_reference_entries;
DROP TABLE IF EXISTS cls_reference_versions;
DROP TYPE IF EXISTS cls_reference_status_enum;

COMMIT;
