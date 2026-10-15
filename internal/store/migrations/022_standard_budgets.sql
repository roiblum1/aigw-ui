-- A model's standard per-tenant budget (docs/design/capacity-prices-wallets.md,
-- "Identity and quota rules", 5b): every tenant may use the model up to it,
-- through one rule on the tenant header, and a tenant's own quota on the
-- model is an exception that adds to it. NULL: no standard, as before.
ALTER TABLE models
    ADD COLUMN standard_limit  bigint CHECK (standard_limit IS NULL OR standard_limit >= 1),
    ADD COLUMN standard_window text CHECK (standard_window IS NULL OR standard_window IN ('1m', '1h', '1d')),
    ADD CONSTRAINT models_standard CHECK ((standard_limit IS NULL) = (standard_window IS NULL)),
    -- A change waiting for its day, as prices do: the new limit and window,
    -- both NULL to remove the standard, and when it starts.
    ADD COLUMN standard_next_limit  bigint CHECK (standard_next_limit IS NULL OR standard_next_limit >= 1),
    ADD COLUMN standard_next_window text CHECK (standard_next_window IS NULL OR standard_next_window IN ('1m', '1h', '1d')),
    ADD COLUMN standard_next_at     timestamptz,
    ADD CONSTRAINT models_standard_next CHECK ((standard_next_limit IS NULL) = (standard_next_window IS NULL));
