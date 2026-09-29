-- 199_funding_flow_extensions.down.sql

BEGIN;

DROP TABLE IF EXISTS withdrawal_destination_holds;
DROP TABLE IF EXISTS nostro_replenishment_requests;
DROP TABLE IF EXISTS funding_ops_alerts;
DROP TABLE IF EXISTS withdrawal_dispatch_queue;
DROP TABLE IF EXISTS deposit_confirmations;

DROP INDEX IF EXISTS funding_tx_deposit_review_ix;
DROP INDEX IF EXISTS funding_tx_dispatch_ix;

ALTER TABLE funding_transactions
    DROP COLUMN IF EXISTS originator_name,
    DROP COLUMN IF EXISTS reviewed_at,
    DROP COLUMN IF EXISTS reviewed_by,
    DROP COLUMN IF EXISTS hold_until;

COMMIT;
