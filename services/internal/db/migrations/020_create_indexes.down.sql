-- 020_create_indexes.down.sql
BEGIN;

DROP INDEX IF EXISTS idx_withdrawal_confirmations_withdrawal_id;
DROP INDEX IF EXISTS idx_accounts_parent_account_id;
DROP INDEX IF EXISTS idx_accounts_user_id;
DROP INDEX IF EXISTS uq_margin_accounts_account_id;
DROP INDEX IF EXISTS idx_kyc_documents_account_id;
DROP INDEX IF EXISTS idx_settlement_instructions_account_id;
DROP INDEX IF EXISTS idx_settlement_instructions_trade_id;
DROP INDEX IF EXISTS idx_positions_account_instrument;
DROP INDEX IF EXISTS idx_admin_audit_log_target;
DROP INDEX IF EXISTS idx_admin_audit_log_admin_user_id;
DROP INDEX IF EXISTS idx_audit_hash_chain_created_at;
DROP INDEX IF EXISTS idx_funding_reference;
DROP INDEX IF EXISTS idx_funding_account_id_status;
DROP INDEX IF EXISTS idx_trades_sell_order_id;
DROP INDEX IF EXISTS idx_trades_buy_order_id;
DROP INDEX IF EXISTS idx_trades_seller_account_id;
DROP INDEX IF EXISTS idx_trades_buyer_account_id;
DROP INDEX IF EXISTS idx_trades_instrument_created;
DROP INDEX IF EXISTS uq_orders_account_client_order_id;
DROP INDEX IF EXISTS idx_orders_client_order_id;
DROP INDEX IF EXISTS idx_orders_instrument_id_status;
DROP INDEX IF EXISTS idx_orders_account_id_status;

ALTER TABLE accounts
    DROP CONSTRAINT IF EXISTS fk_accounts_fee_tier_id,
    DROP CONSTRAINT IF EXISTS fk_accounts_risk_limits_id;

COMMIT;
