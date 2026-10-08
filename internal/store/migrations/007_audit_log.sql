-- The audit log: one row per request that changes something, or tries to.
-- Request bodies are never stored: they can hold kubeconfigs and tokens.
CREATE TABLE audit_log (
    id            bigserial PRIMARY KEY,
    at            timestamptz NOT NULL DEFAULT now(),
    -- How the caller was authenticated. There is one admin token today.
    actor         text NOT NULL,
    -- The name the caller gave in the X-On-Behalf-Of header. Not verified.
    on_behalf_of  text NOT NULL DEFAULT '',
    method        text NOT NULL,
    path          text NOT NULL,
    status        integer NOT NULL,
    action        text NOT NULL DEFAULT '',
    summary       text NOT NULL DEFAULT '',
    remote_addr   text NOT NULL DEFAULT '',
    forwarded_for text NOT NULL DEFAULT '',
    user_agent    text NOT NULL DEFAULT '',
    duration_ms   integer NOT NULL DEFAULT 0
);
