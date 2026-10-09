-- The entry route: each fleet cluster's gateway can send a request for a
-- model to any site that serves it.
ALTER TABLE clusters
    -- Where the other sites reach this site's gateway, for example
    -- llm.site1-a.example.com and 8443.
    ADD COLUMN peer_host      text NOT NULL DEFAULT '',
    ADD COLUMN peer_port      integer NOT NULL DEFAULT 8443,
    -- render.FleetRevision of what was last applied to the cluster.
    ADD COLUMN fleet_revision text NOT NULL DEFAULT '';

ALTER TABLE models
    -- The hub renders the model's entry route on every fleet cluster.
    ADD COLUMN fleet boolean NOT NULL DEFAULT false;
