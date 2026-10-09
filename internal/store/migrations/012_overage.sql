-- What happens to a tenant whose budget for a model is spent.
ALTER TABLE models
    -- 'refuse' answers 429. 'best-effort' moves the tenant's requests for
    -- the model to a second entry route that never refuses them and marks
    -- them as the lowest class.
    ADD COLUMN spent_mode text NOT NULL DEFAULT 'refuse'
        CONSTRAINT models_spent_mode CHECK (spent_mode IN ('refuse', 'best-effort')),
    -- Tenants without a quota of their own on the model run as best-effort
    -- too, in place of sharing the model's default pool.
    ADD COLUMN best_effort_unlimited boolean NOT NULL DEFAULT false;

-- A tenant that is being served as best-effort on a model, from when it was
-- moved until its quota window ends. Rows are kept as the history.
CREATE TABLE overage (
    model_id   uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    since      timestamptz NOT NULL DEFAULT now(),
    until      timestamptz NOT NULL,
    PRIMARY KEY (model_id, tenant_id, since)
);
CREATE INDEX overage_until ON overage (until);
