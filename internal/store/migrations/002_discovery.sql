-- Models can now be discovered from the AIGatewayRoutes that already exist on a
-- cluster. A discovered endpoint has no host or port: it points at the
-- AIServiceBackends the existing route uses.
ALTER TABLE model_endpoints
    ADD COLUMN source text NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'discovered')),
    ADD COLUMN backends jsonb NOT NULL DEFAULT '[]',
    ALTER COLUMN host SET DEFAULT '',
    ALTER COLUMN port SET DEFAULT 0,
    DROP CONSTRAINT model_endpoints_port_check,
    ADD CONSTRAINT model_endpoints_port_check CHECK (source = 'discovered' OR port BETWEEN 1 AND 65535);

ALTER TABLE clusters
    ADD COLUMN discovery_message text NOT NULL DEFAULT '',
    ADD COLUMN discovered_at timestamptz;
