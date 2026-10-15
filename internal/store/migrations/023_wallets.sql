-- Wallets (docs/design/capacity-prices-wallets.md, "Wallets" and 5c): one
-- budget for a tenant across every priced model, per hour or per day, in
-- credits. A tenant's quotas on priced models are caps inside it.
ALTER TABLE tenants
    ADD COLUMN wallet_credits bigint CHECK (wallet_credits IS NULL OR wallet_credits >= 1),
    ADD COLUMN wallet_window text CHECK (wallet_window IS NULL OR wallet_window IN ('1h', '1d')),
    ADD CONSTRAINT tenants_wallet CHECK ((wallet_credits IS NULL) = (wallet_window IS NULL));

-- A quota the hub keeps equal to the tenant's wallet, on a priced model it
-- has no cap on: the tenant can use the model up to its wallet, and the
-- model keeps a counter of it. Setting a quota by hand makes it a cap.
ALTER TABLE quotas ADD COLUMN wallet boolean NOT NULL DEFAULT false;

-- A tenant whose wallet is spent, until its window ends: the gateways refuse
-- it on every priced model, or serve it as best-effort where that is on.
CREATE TABLE wallet_closures (
    tenant_id uuid PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,
    until     timestamptz NOT NULL,
    spent     bigint NOT NULL,
    closed_at timestamptz NOT NULL DEFAULT now()
);
