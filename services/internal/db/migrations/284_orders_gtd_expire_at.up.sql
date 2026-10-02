-- Task 6 item 2 (spec §27 R8): GTC max order lifetime — 90 calendar
-- days. The orders row must persist the effective GTD expiry (the R8
-- cap or the client-supplied date) so the read model reflects the
-- engine's expiry and UpdateField's gtd_expire_at path resolves to a
-- real column (previously referenced but never created — latent
-- runtime error on any TIF/expiry amend).
ALTER TABLE orders ADD COLUMN gtd_expire_at TIMESTAMPTZ NULL;

-- Read-model sweep aids: expiry lookups (rebuild after WAL replay,
-- admin "orders expiring soon" views) filter on non-null expiries.
CREATE INDEX idx_orders_gtd_expire_at ON orders (gtd_expire_at)
    WHERE gtd_expire_at IS NOT NULL;
