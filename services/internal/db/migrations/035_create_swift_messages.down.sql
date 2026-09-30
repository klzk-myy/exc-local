-- 035_create_swift_messages.down.sql — Phase-24 Task 24.3.4.

BEGIN;

DROP TRIGGER IF EXISTS swift_messages_no_update ON swift_messages;
DROP TRIGGER IF EXISTS swift_messages_no_delete ON swift_messages;
DROP FUNCTION IF EXISTS swift_messages_immutable();
DROP TABLE IF EXISTS swift_messages;
DROP TYPE IF EXISTS swift_direction_enum;

COMMIT;
