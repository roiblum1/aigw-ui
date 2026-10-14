-- How far the tenants' budgets on a model may add up beyond what its GPUs
-- serve a day at the expected busy share. Budgets above it are allowed; the
-- interface warns and asks to confirm.
ALTER TABLE models ADD COLUMN overcommit_limit double precision NOT NULL DEFAULT 1.5
    CHECK (overcommit_limit >= 1 AND overcommit_limit <= 10);
