-- A model's prices, in credits per million tokens. One credit is a
-- hundred-thousandth of a dollar. A model without an applied row is counted
-- in tokens.
CREATE TABLE model_prices (
    model_id     uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    version      integer NOT NULL,
    price_input  bigint NOT NULL CHECK (price_input > 0),
    -- The part of a prompt the model took from its prefix cache.
    price_cached bigint NOT NULL CHECK (price_cached >= 0),
    price_output bigint NOT NULL CHECK (price_output >= 0),
    -- Prices start at the beginning of a day, when every quota window starts
    -- anew, so no counter holds amounts at two prices.
    effective_at timestamptz NOT NULL,
    -- Set when the server has started rendering these prices.
    applied_at   timestamptz,
    note         text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (model_id, version)
);
-- At most one set of prices waits for its day.
CREATE UNIQUE INDEX model_prices_pending ON model_prices (model_id) WHERE applied_at IS NULL;

-- A priced model in dry-run counts every tenant and refuses nobody.
ALTER TABLE models ADD COLUMN price_dry_run boolean NOT NULL DEFAULT false;
