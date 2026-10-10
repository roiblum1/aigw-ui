-- What a tenant used of a model in each hour, as the gateways counted it.
-- The counters in Redis only hold the running period, so this is the only
-- record of past usage. It holds amounts and nothing of a request.
CREATE TABLE usage_hours (
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    model_id    uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    hour        timestamptz NOT NULL,
    -- A model counted in tokens that gets prices is counted in credits from
    -- then on. The two are never added up.
    unit        text NOT NULL CHECK (unit IN ('tokens', 'credits')),
    -- Within the tenant's budget.
    used        bigint NOT NULL DEFAULT 0,
    -- As best-effort, after the budget was spent.
    best_effort bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, model_id, hour, unit)
);
CREATE INDEX usage_hours_hour ON usage_hours (hour);

-- The counter values at the last look, to tell what was used since.
CREATE TABLE usage_cursors (
    tenant_id    uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    model_id     uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    window_size  text NOT NULL,
    window_start timestamptz NOT NULL,
    used         bigint NOT NULL,
    best_effort  bigint NOT NULL,
    PRIMARY KEY (tenant_id, model_id)
);

-- The most a tenant may use of a model as best-effort in one period of its
-- quota. NULL is no limit.
ALTER TABLE models ADD COLUMN best_effort_limit bigint CHECK (best_effort_limit > 0);

-- Set when a tenant reached that limit: it is off the best-effort route for
-- the rest of the period.
ALTER TABLE overage ADD COLUMN capped_at timestamptz;
