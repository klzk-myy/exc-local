-- 108_suspense_accounts_routing — down.

BEGIN;

DROP TABLE IF EXISTS rail_payments;
DROP TABLE IF EXISTS suspense_account_mappings;

DROP TYPE IF EXISTS rail_payment_status_enum;
DROP TYPE IF EXISTS quarantine_status_enum;
DROP TYPE IF EXISTS unmatched_reason_enum;

COMMIT;
