-- A model's standard budget is gone (0.26.0): a tenant is held by a budget
-- across models or by quotas of its own, which take one gateway rule per
-- amount, so a default for every tenant no longer saves rules. A tenant
-- with neither is held by the model's pool.
ALTER TABLE models
    DROP CONSTRAINT models_standard,
    DROP CONSTRAINT models_standard_next,
    DROP COLUMN standard_limit,
    DROP COLUMN standard_window,
    DROP COLUMN standard_next_limit,
    DROP COLUMN standard_next_window,
    DROP COLUMN standard_next_at;
