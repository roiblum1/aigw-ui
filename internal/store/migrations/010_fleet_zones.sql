-- The zone weights of each model, worked out once for the whole fleet on
-- each discovery round and rendered the same to every cluster.
ALTER TABLE models
    -- [{"zone": "<cluster name>", "weight": 800}, ...], sorted by zone. Only
    -- the sites that serve the model and are not drained out.
    ADD COLUMN fleet_zones jsonb NOT NULL DEFAULT '[]',
    -- Why fleet_zones was not replaced on the last round, when it was not.
    ADD COLUMN fleet_error text NOT NULL DEFAULT '';

ALTER TABLE model_endpoints
    -- A drained site whose weight has reached the floor leaves the model's
    -- zones on the next poll. Cleared when the drain ends.
    ADD COLUMN unlisted boolean NOT NULL DEFAULT false;
