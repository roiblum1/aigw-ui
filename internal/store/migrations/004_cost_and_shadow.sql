-- A model can weight token types differently when its quota is charged, and a
-- tenant's quota can be tried out without rejecting requests.
ALTER TABLE models ADD COLUMN cost_expression text NOT NULL DEFAULT '';
ALTER TABLE quotas ADD COLUMN shadow boolean NOT NULL DEFAULT false;
