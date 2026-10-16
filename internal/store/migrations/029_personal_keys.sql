-- A personal key: a secret a user's helper trades at the hub for a token
-- that lives a few days (POST /api/v1/token), signed with the tenant's
-- budget and models as they are then. The hub keeps only its hash, and the
-- gateways never see it. Its jti is the part of its client ID after the
-- tenant's slug, which every token it gets carries as cid; expires_at is
-- when the last of those tokens expires.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_kind_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_kind_check CHECK (kind IN ('key', 'token', 'personal'));
ALTER TABLE api_keys ALTER COLUMN key_enc DROP NOT NULL;
ALTER TABLE api_keys
    ADD COLUMN key_hash bytea UNIQUE,
    ADD COLUMN renewed_at timestamptz,
    ADD CONSTRAINT api_keys_secret CHECK (kind = 'personal' OR key_enc IS NOT NULL),
    ADD CONSTRAINT api_keys_personal CHECK (kind <> 'personal' OR (key_hash IS NOT NULL AND jti IS NOT NULL));
ALTER TABLE api_keys DROP CONSTRAINT api_keys_token;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_token
    CHECK (kind <> 'token' OR (jti IS NOT NULL AND signing_key_id IS NOT NULL AND expires_at IS NOT NULL));

-- A retired signing key stays in the gateways' key set until the last token
-- it signed for a personal key expires; those tokens are not stored.
ALTER TABLE signing_keys ADD COLUMN renewed_until timestamptz;

-- Clients the gateways must refuse although their tokens are signed: a
-- revoked or deleted personal key's, until the last token it got expires.
CREATE TABLE client_denials (
    client_id  text PRIMARY KEY,
    expires_at timestamptz NOT NULL
);

CREATE OR REPLACE FUNCTION deny_token() RETURNS trigger AS $$
BEGIN
    IF OLD.revoked_at IS NULL AND OLD.expires_at > now() AND (TG_OP = 'DELETE' OR NEW.revoked_at IS NOT NULL) THEN
        IF OLD.kind = 'token' THEN
            INSERT INTO token_denials (jti, expires_at) VALUES (OLD.jti, OLD.expires_at) ON CONFLICT DO NOTHING;
        ELSIF OLD.kind = 'personal' THEN
            INSERT INTO client_denials (client_id, expires_at) VALUES (OLD.client_id, OLD.expires_at)
            ON CONFLICT (client_id) DO UPDATE SET expires_at = greatest(client_denials.expires_at, EXCLUDED.expires_at);
        END IF;
    END IF;
    RETURN NULL;
END $$ LANGUAGE plpgsql;
