-- 062_comms_recordings.down.sql — reverses Task 21.3.20.
--
-- Order matters: drop the capture triggers first (they are installed on
-- pre-existing tables), then the functions, then the task-owned tables.
-- comms_recordings rows may still be inside their 5-year retention — the
-- row guard trigger fires on DELETE, not DROP TABLE, so the drop
-- succeeds; that mirrors the repo convention that down migrations are
-- dev/teardown tools, not a runtime path (documented in header).

BEGIN;

DROP TRIGGER IF EXISTS trg_comms_capture_ticket_note ON ticket_notes;
DROP FUNCTION IF EXISTS comms_capture_ticket_note();
DROP TRIGGER IF EXISTS trg_comms_capture_ticket ON support_tickets;
DROP FUNCTION IF EXISTS comms_capture_ticket();
DROP TRIGGER IF EXISTS trg_comms_capture_delivery ON notification_deliveries;
DROP FUNCTION IF EXISTS comms_capture_delivery();

DROP TRIGGER IF EXISTS trg_comms_access_guard ON comms_recording_access;
DROP TRIGGER IF EXISTS trg_comms_recordings_guard ON comms_recordings;
DROP FUNCTION IF EXISTS comms_recording_access_guard();
DROP FUNCTION IF EXISTS comms_recordings_guard();

DROP TABLE IF EXISTS comms_recording_access;
DROP TABLE IF EXISTS comms_capture_outbox;
DROP TABLE IF EXISTS comms_recordings;

COMMIT;
