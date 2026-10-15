-- Wallets (023) are removed: a tenant's budgets are per model only, as
-- before 0.20.0. A wallet bucket stays as the tenant's budget on its model,
-- of the same amount and window, so no limit on a gateway changes; a
-- closure ends.
DROP TABLE wallet_closures;

ALTER TABLE quotas DROP COLUMN wallet;

ALTER TABLE tenants
    DROP CONSTRAINT tenants_wallet,
    DROP COLUMN wallet_credits,
    DROP COLUMN wallet_window;
