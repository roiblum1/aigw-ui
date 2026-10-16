-- A tenant keeps its position in the gateways' budget policy when its
-- budget is removed: the position is part of the counter's name, so a budget
-- set again comes back with what the tenant used in the running period. A
-- position held without a budget goes to another tenant only when every
-- position is held.
ALTER TABLE tenants DROP CONSTRAINT tenants_budget_has_slot;
ALTER TABLE tenants ADD CONSTRAINT tenants_budget_has_slot CHECK (budget_credits IS NULL OR budget_slot IS NOT NULL);
