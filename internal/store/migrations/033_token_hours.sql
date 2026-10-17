-- What every counter the history reads held at the last look, by its Redis
-- key. A counter whose rule moved, or one a token with an older amount still
-- counts in, keeps its own cursor and is read to the end of its window. Only
-- counters that held something have one. usage_cursors, one cursor per
-- tenant and model, is read once more by the first look after the upgrade.
CREATE TABLE usage_counter_cursors (
    redis_key    text PRIMARY KEY,
    tenant_id    uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    model_id     uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    -- 'used' and 'best_effort' are added to usage_hours; 'requests',
    -- 'input', 'cached' and 'output' to token_hours.
    kind         text NOT NULL,
    -- The unit of a 'used' or 'best_effort' counter, empty for the others.
    unit         text NOT NULL DEFAULT '',
    window_size  text NOT NULL,
    window_start timestamptz NOT NULL,
    value        bigint NOT NULL
);

-- What a tenant sent to a model in each hour, as the gateways counted it:
-- its requests, and the input (cached ones included), cached and output
-- tokens of their answers. Numbers only, nothing of a request.
CREATE TABLE token_hours (
    tenant_id     uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    model_id      uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    hour          timestamptz NOT NULL,
    requests      bigint NOT NULL DEFAULT 0,
    input_tokens  bigint NOT NULL DEFAULT 0,
    cached_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, model_id, hour)
);
CREATE INDEX token_hours_hour ON token_hours (hour);
