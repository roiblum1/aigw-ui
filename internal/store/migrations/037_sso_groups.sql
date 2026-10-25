-- A tenant may be linked to a group of the people who sign in with
-- OpenShift (the hub cluster's Groups, synced from LDAP). Its members sign
-- in on the tenants' page, get a personal key of their own and see every
-- member's use. NULL: a tenant as before, with no sign-in but its keys.
ALTER TABLE tenants ADD COLUMN sso_group text;
CREATE UNIQUE INDEX tenants_sso_group ON tenants (sso_group) WHERE sso_group IS NOT NULL;

-- The person a personal key was made for, by their OpenShift user name,
-- when they made it themselves through the sign-in. One live key each per
-- tenant: a new one replaces the old. The key is revoked when its owner
-- leaves the tenant's group.
ALTER TABLE api_keys ADD COLUMN owner text;
CREATE UNIQUE INDEX api_keys_owner ON api_keys (tenant_id, owner) WHERE owner IS NOT NULL AND revoked_at IS NULL;

-- A cursor of a counter the gateways keep for one key (by its client ID)
-- rather than for its tenant: the history adds it to key_token_hours.
ALTER TABLE usage_counter_cursors ADD COLUMN key_id uuid REFERENCES api_keys (id) ON DELETE CASCADE;

-- What each key sent to a model in each hour, as the gateways counted it,
-- like token_hours for the tenant. Numbers only, nothing of a request.
CREATE TABLE key_token_hours (
    key_id        uuid NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    model_id      uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    hour          timestamptz NOT NULL,
    requests      bigint NOT NULL DEFAULT 0,
    input_tokens  bigint NOT NULL DEFAULT 0,
    cached_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, model_id, hour)
);
CREATE INDEX key_token_hours_hour ON key_token_hours (hour);
