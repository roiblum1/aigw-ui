-- The position of a tenant's rule in the gateways' budget policy. The
-- position is part of the name of the rule's counter, so a tenant keeps it
-- while it has a budget; ordered by slug, every tenant after a new one moved
-- onto its neighbour's counter. Today's budgets get the positions they
-- already have among the enabled tenants, ordered by slug, and the disabled
-- ones the positions after them.
ALTER TABLE tenants ADD COLUMN budget_slot integer CHECK (budget_slot IS NULL OR budget_slot >= 0);

UPDATE tenants t SET budget_slot = n.slot
FROM (SELECT id, row_number() OVER (ORDER BY NOT enabled, slug) - 1 AS slot
      FROM tenants WHERE budget_credits IS NOT NULL) n
WHERE t.id = n.id;

CREATE UNIQUE INDEX tenants_budget_slot ON tenants (budget_slot) WHERE budget_slot IS NOT NULL;
ALTER TABLE tenants ADD CONSTRAINT tenants_budget_has_slot CHECK ((budget_credits IS NULL) = (budget_slot IS NULL));
