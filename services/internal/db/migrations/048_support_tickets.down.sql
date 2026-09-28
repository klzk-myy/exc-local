-- 048_support_tickets.down.sql
BEGIN;

DROP TABLE IF EXISTS ticket_notes CASCADE;
DROP TABLE IF EXISTS support_tickets CASCADE;
DROP TYPE IF EXISTS support_ticket_status_enum;
DROP TYPE IF EXISTS support_ticket_priority_enum;
DROP TYPE IF EXISTS support_ticket_category_enum;
DROP TYPE IF EXISTS support_ticket_type_enum;

COMMIT;
