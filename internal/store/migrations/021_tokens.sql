-- Tokens the hub signs, which tenants use as their API keys
-- (docs/design/capacity-prices-wallets.md, "Identity and quota rules", 5a).

-- The hub's signing keys. One signs new tokens; the others are retired and
-- stay in the gateways' key set while a token they signed is still valid.
CREATE TABLE signing_keys (
    id          text PRIMARY KEY,           -- the kid
    private_enc bytea NOT NULL,             -- the EC private key, encrypted with ENCRYPTION_KEY
    public_jwk  jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    retired_at  timestamptz
);
CREATE UNIQUE INDEX signing_keys_one_active ON signing_keys ((retired_at IS NULL)) WHERE retired_at IS NULL;

-- A key is an API key ("key", the kind there was before) or a token. A
-- token's jti is the part of its client ID after the tenant's slug.
ALTER TABLE api_keys
    ADD COLUMN kind text NOT NULL DEFAULT 'key' CHECK (kind IN ('key', 'token')),
    ADD COLUMN jti text UNIQUE,
    ADD COLUMN signing_key_id text REFERENCES signing_keys (id),
    ADD COLUMN expires_at timestamptz,
    ADD CONSTRAINT api_keys_token CHECK (kind = 'key' OR (jti IS NOT NULL AND signing_key_id IS NOT NULL AND expires_at IS NOT NULL));

-- Tokens the gateways must refuse although their signature is good: revoked,
-- or of a tenant that was deleted. A row is kept until the token expires,
-- after which the gateways refuse it anyway; it outlives the tenant's rows.
CREATE TABLE token_denials (
    jti        text PRIMARY KEY,
    expires_at timestamptz NOT NULL
);

CREATE FUNCTION deny_token() RETURNS trigger AS $$
BEGIN
    IF OLD.kind = 'token' AND OLD.revoked_at IS NULL AND OLD.expires_at > now()
       AND (TG_OP = 'DELETE' OR NEW.revoked_at IS NOT NULL) THEN
        INSERT INTO token_denials (jti, expires_at) VALUES (OLD.jti, OLD.expires_at) ON CONFLICT DO NOTHING;
    END IF;
    RETURN NULL;
END $$ LANGUAGE plpgsql;

-- Revoking a token, and deleting it with its tenant, both deny it.
CREATE TRIGGER api_keys_deny AFTER UPDATE OF revoked_at OR DELETE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION deny_token();
