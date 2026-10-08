-- A tenant quota becomes one bucket rule of the model's QuotaPolicy, and the
-- position of that rule is part of the name of its counter in Redis. The
-- position is stored so it never moves: before this, rules were ordered by
-- tenant slug, and adding or removing a quota restarted the counters of the
-- tenants after it.
ALTER TABLE quotas ADD COLUMN slot integer;

-- Existing quotas keep the position they are rendered at today: enabled
-- tenants ordered by slug. Disabled tenants are not rendered and go last.
UPDATE quotas q SET slot = r.slot
FROM (
    SELECT q2.id,
           row_number() OVER (PARTITION BY q2.model_id ORDER BY NOT t.enabled, t.slug COLLATE "C") - 1 AS slot
    FROM quotas q2 JOIN tenants t ON t.id = q2.tenant_id
) r
WHERE r.id = q.id;

ALTER TABLE quotas ALTER COLUMN slot SET NOT NULL;
ALTER TABLE quotas ADD CONSTRAINT quotas_model_slot UNIQUE (model_id, slot);
ALTER TABLE quotas ADD CONSTRAINT quotas_slot_range CHECK (slot >= 0);
