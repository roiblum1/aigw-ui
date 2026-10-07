CREATE TABLE clusters (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text NOT NULL UNIQUE,
    site           text NOT NULL DEFAULT '',
    namespace      text NOT NULL,
    gateway_name   text NOT NULL,
    auth_enabled   boolean NOT NULL DEFAULT false,
    kubeconfig_enc bytea NOT NULL,
    sync_status    text NOT NULL DEFAULT 'pending',
    sync_message   text NOT NULL DEFAULT '',
    synced_at      timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE models (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name           text NOT NULL UNIQUE,
    slug           text NOT NULL UNIQUE,
    default_limit  bigint NOT NULL DEFAULT 1 CHECK (default_limit > 0),
    default_window text NOT NULL DEFAULT '1d' CHECK (default_window IN ('1m', '1h', '1d')),
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE model_endpoints (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id       uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    cluster_id     uuid NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
    host           text NOT NULL,
    port           integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    upstream_model text NOT NULL,
    UNIQUE (model_id, cluster_id)
);

CREATE TABLE tenants (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         text NOT NULL UNIQUE,
    display_name text NOT NULL DEFAULT '',
    enabled      boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       text NOT NULL DEFAULT '',
    client_id  text NOT NULL UNIQUE,
    key_prefix text NOT NULL,
    key_enc    bytea NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE quotas (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    model_id    uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    token_limit bigint NOT NULL CHECK (token_limit > 0),
    window_size text NOT NULL CHECK (window_size IN ('1m', '1h', '1d')),
    UNIQUE (tenant_id, model_id)
);
