-- Fleet clusters are the sites that share traffic for a model. Each one is a
-- zone in every model's site weights, with weight 0 where it does not serve
-- the model.
ALTER TABLE clusters
    ADD COLUMN fleet_enabled   boolean NOT NULL DEFAULT false,
    -- The Gateway listener clients come in on. The API-key policy attaches to
    -- it alone when set, and to the whole Gateway when empty.
    ADD COLUMN client_listener text NOT NULL DEFAULT '';

ALTER TABLE model_endpoints
    -- The cluster has an LLMInferenceService for the model.
    ADD COLUMN serving       boolean NOT NULL DEFAULT false,
    -- Set by an operator: the site's weight steps down to 0 and stays there.
    ADD COLUMN drained       boolean NOT NULL DEFAULT false,
    ADD COLUMN revision      text NOT NULL DEFAULT '',
    ADD COLUMN max_model_len text NOT NULL DEFAULT '';
