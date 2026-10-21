-- A token a personal key got while the gateways take API keys too. Their
-- API key check runs first and lets through only the credentials it lists,
-- so such a token is stored, encrypted, to be listed with the API keys
-- (Secret aigw-ui-api-keys, under the tenant's slug and the token's jti).
-- With tokens only, the gateways check a token's signature and nothing is
-- stored. The key's helper is given the newest of them that has had time
-- to reach the gateways, so a renewal does not change what is listed.
CREATE TABLE personal_tokens (
    jti        text PRIMARY KEY,
    key_id     uuid NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    token_enc  bytea NOT NULL,
    issued_at  timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE INDEX personal_tokens_key ON personal_tokens (key_id, issued_at DESC);
