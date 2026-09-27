-- 020_create_indexes.up.sql
-- All performance indexes for the §5 baseline schema + deferred FK back-refs
-- (accounts.risk_limits_id → risk_limits, accounts.fee_tier_id → fee_tiers,
-- both tables exist by now).
-- Indexes on the partitioned trades parent propagate to all partitions.

BEGIN;

-- Deferred FK constraints for accounts (could not be declared in 003 —
-- risk_limits and fee_tiers did not exist yet).
ALTER TABLE accounts
    ADD CONSTRAINT fk_accounts_risk_limits_id FOREIGN KEY (risk_limits_id) REFERENCES risk_limits (id),
    ADD CONSTRAINT fk_accounts_fee_tier_id    FOREIGN KEY (fee_tier_id)    REFERENCES fee_tiers (id);

-- orders: hot lookup paths — account order book, instrument book, client id lookup.
CREATE INDEX idx_orders_account_id_status    ON orders (account_id, status);
CREATE INDEX idx_orders_instrument_id_status ON orders (instrument_id, status);
CREATE INDEX idx_orders_client_order_id      ON orders (client_order_id);

-- Idempotent order submission (spec §8.1 / §5.4a companion):
-- one active client_order_id value per account.
CREATE UNIQUE INDEX uq_orders_account_client_order_id
    ON orders (account_id, client_order_id)
    WHERE client_order_id IS NOT NULL;

-- trades (partitioned parent — propagates to children).
CREATE INDEX idx_trades_instrument_created ON trades (instrument_id, created_at);
CREATE INDEX idx_trades_buyer_account_id   ON trades (buyer_account_id);
CREATE INDEX idx_trades_seller_account_id  ON trades (seller_account_id);
CREATE INDEX idx_trades_buy_order_id       ON trades (buy_order_id);
CREATE INDEX idx_trades_sell_order_id      ON trades (sell_order_id);

-- funding_transactions: account funding queue + status sweeps.
CREATE INDEX idx_funding_account_id_status ON funding_transactions (account_id, status);
CREATE INDEX idx_funding_reference         ON funding_transactions (reference);

-- audit_hash_chain: time-ordered chain scans / daily Merkle computation.
CREATE INDEX idx_audit_hash_chain_created_at ON audit_hash_chain (created_at);

-- admin_audit_log: per-admin and per-target lookups.
CREATE INDEX idx_admin_audit_log_admin_user_id ON admin_audit_log (admin_user_id);
CREATE INDEX idx_admin_audit_log_target        ON admin_audit_log (target_type, target_id);

-- positions: all open positions for an account/instrument (non-unique —
-- spec does not constrain one row per account+instrument; hedging-mode
-- accounts may hold LONG and SHORT simultaneously).
CREATE INDEX idx_positions_account_instrument ON positions (account_id, instrument_id);

-- settlement_instructions: per-trade settlement legs.
CREATE INDEX idx_settlement_instructions_trade_id   ON settlement_instructions (trade_id);
CREATE INDEX idx_settlement_instructions_account_id ON settlement_instructions (account_id);

-- kyc_documents: per-account document list.
CREATE INDEX idx_kyc_documents_account_id ON kyc_documents (account_id);

-- margin_accounts: exactly one margin account per trading account.
CREATE UNIQUE INDEX uq_margin_accounts_account_id ON margin_accounts (account_id);

-- accounts: per-user account listing + sub-account tree walk.
CREATE INDEX idx_accounts_user_id           ON accounts (user_id);
CREATE INDEX idx_accounts_parent_account_id ON accounts (parent_account_id);

-- withdrawal_confirmations: pending confirmations per withdrawal.
CREATE INDEX idx_withdrawal_confirmations_withdrawal_id ON withdrawal_confirmations (withdrawal_id);

COMMIT;
