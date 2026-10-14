-- What a site's capacity of a model is counted in: the annotation
-- aigw-ui.io/capacity-per-instance ("declared"), or the measured throughput
-- of its instances' shapes ("measured"). A model's sites are all one or all
-- the other, so their weights compare.
ALTER TABLE model_endpoints ADD COLUMN capacity_basis text NOT NULL DEFAULT 'declared'
    CHECK (capacity_basis IN ('declared', 'measured'));
