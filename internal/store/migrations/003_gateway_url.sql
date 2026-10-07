-- A cluster can name the address of its gateway. Models are then listed from
-- the gateway's own /v1/models endpoint instead of from one namespace's routes.
-- The token is only needed when the gateway requires a key for that endpoint.
ALTER TABLE clusters
    ADD COLUMN gateway_url text NOT NULL DEFAULT '',
    ADD COLUMN discovery_token_enc bytea;
