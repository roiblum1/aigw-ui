-- A tenant signs in to its own page with an API key, which is found by its
-- first characters.
CREATE INDEX IF NOT EXISTS api_keys_prefix ON api_keys (key_prefix);

-- Deleting a model deletes its history, which is found by the model then
-- rather than by reading all of it.
CREATE INDEX IF NOT EXISTS usage_hours_model ON usage_hours (model_id);
CREATE INDEX IF NOT EXISTS token_hours_model ON token_hours (model_id);
