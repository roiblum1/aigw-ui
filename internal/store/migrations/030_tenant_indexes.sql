-- A tenant's keys and best-effort periods are looked up by tenant: the
-- tenants list counts each tenant's keys, and deleting a tenant removes its
-- rows from both. Without these, every one of those looks read the whole
-- table, which with 1000 tenants made the tenants list take 0.1 s.
CREATE INDEX IF NOT EXISTS api_keys_tenant ON api_keys (tenant_id);
CREATE INDEX IF NOT EXISTS overage_tenant ON overage (tenant_id);
