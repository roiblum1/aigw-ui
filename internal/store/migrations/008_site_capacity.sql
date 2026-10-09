-- What each cluster can serve of each model, read from the cluster, and the
-- value that is applied as the site's weight. The two differ on purpose: the
-- applied value follows the observed one slowly (see internal/weights).
ALTER TABLE model_endpoints
    -- NULL means the cluster has not reported it: unknown, never zero.
    ADD COLUMN capacity_observed    double precision,
    ADD COLUMN capacity_step        double precision NOT NULL DEFAULT 1,
    ADD COLUMN capacity_detail      text NOT NULL DEFAULT '',
    ADD COLUMN capacity_observed_at timestamptz,
    -- NULL until the first observation.
    ADD COLUMN capacity_applied     double precision,
    ADD COLUMN capacity_low_streak  integer NOT NULL DEFAULT 0,
    ADD COLUMN capacity_changed_at  timestamptz;
