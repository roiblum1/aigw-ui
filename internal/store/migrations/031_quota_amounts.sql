-- Once the gateways take tokens only, the tenants' own quotas on a model are
-- rendered as one rule per amount instead of one per tenant: the tenants
-- with the same limit, window and shadow setting on a model share a rule,
-- which counts each of them on its own. The token names the tenant's amount
-- on each model it has an own quota on.
--
-- The position of an amount's rule is part of the names of its counters in
-- Redis, so it is stored and never moves while the amount is in use, as
-- quotas.slot is for a tenant's rule. A trigger keeps the table in step with
-- quotas: a new amount takes the lowest free position, and an amount no
-- quota has any more frees its position.
CREATE TABLE quota_amounts (
    model_id    uuid    NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    slot        integer NOT NULL CHECK (slot >= 0),
    token_limit bigint  NOT NULL,
    window_size text    NOT NULL,
    shadow      boolean NOT NULL,
    PRIMARY KEY (model_id, slot),
    CONSTRAINT quota_amounts_amount UNIQUE (model_id, token_limit, window_size, shadow)
);

INSERT INTO quota_amounts (model_id, slot, token_limit, window_size, shadow)
SELECT model_id, row_number() OVER (PARTITION BY model_id ORDER BY window_size, token_limit, shadow) - 1, token_limit, window_size, shadow
FROM (SELECT DISTINCT model_id, token_limit, window_size, shadow FROM quotas) a;

CREATE FUNCTION quota_amounts_sync() RETURNS trigger AS $$
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        DELETE FROM quota_amounts a
        WHERE a.model_id = OLD.model_id AND a.token_limit = OLD.token_limit
          AND a.window_size = OLD.window_size AND a.shadow = OLD.shadow
          AND NOT EXISTS (SELECT 1 FROM quotas q
                          WHERE q.model_id = OLD.model_id AND q.token_limit = OLD.token_limit
                            AND q.window_size = OLD.window_size AND q.shadow = OLD.shadow);
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        INSERT INTO quota_amounts (model_id, slot, token_limit, window_size, shadow)
        SELECT NEW.model_id, min(free), NEW.token_limit, NEW.window_size, NEW.shadow
        FROM generate_series(0, (SELECT count(*) FROM quota_amounts WHERE model_id = NEW.model_id)::int) free
        WHERE NOT EXISTS (SELECT 1 FROM quota_amounts a WHERE a.model_id = NEW.model_id AND a.slot = free)
        ON CONFLICT ON CONSTRAINT quota_amounts_amount DO NOTHING;
    END IF;
    RETURN NULL;
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER quota_amounts_sync AFTER INSERT OR UPDATE OR DELETE ON quotas
    FOR EACH ROW EXECUTE FUNCTION quota_amounts_sync();
