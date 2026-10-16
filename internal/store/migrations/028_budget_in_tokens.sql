-- A tenant's budget across models and its list of models travel in its
-- tokens, and the gateways hold every tenant with one rule, so a budget no
-- longer has a position in the gateways' policy.
ALTER TABLE tenants DROP CONSTRAINT tenants_budget_has_slot;
DROP INDEX tenants_budget_slot;
ALTER TABLE tenants DROP COLUMN budget_slot;

-- When the tenant's budget or list of models last changed: a token signed
-- before it carries the ones from before.
ALTER TABLE tenants ADD COLUMN limits_changed_at timestamptz;
