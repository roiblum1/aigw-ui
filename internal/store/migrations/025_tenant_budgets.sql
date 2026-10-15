-- A tenant's budget across every priced model, enforced by the gateways
-- (one shared rate limit rule per tenant, counted in the hub's Redis), and
-- the models a tenant may call.

-- The budget, in credits for each window. NULL: none, as before.
ALTER TABLE tenants
    ADD COLUMN budget_credits bigint CHECK (budget_credits IS NULL OR budget_credits >= 1),
    ADD COLUMN budget_window  text CHECK (budget_window IS NULL OR budget_window IN ('1m', '1h', '1d')),
    ADD CONSTRAINT tenants_budget CHECK ((budget_credits IS NULL) = (budget_window IS NULL));

-- A restricted tenant may call only the models in tenant_models; any other
-- tenant may call every model, as before.
ALTER TABLE tenants ADD COLUMN restricted boolean NOT NULL DEFAULT false;

CREATE TABLE tenant_models (
    tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    model_id  uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    PRIMARY KEY (tenant_id, model_id)
);
