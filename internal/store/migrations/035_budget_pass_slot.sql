-- The position of the rule that lets a tenant with a budget across models
-- through on a model (the budget-pass rule). Its position is part of the
-- names of the counters it keeps, one per such tenant, which the usage
-- history and the tenant's page read. It was the last rule, so it moved
-- whenever an amount was added after it or the last one was freed: what a
-- tenant used under the old position since the history's last look was
-- lost, and the page showed the model's use start again from 0 (0.31.0
-- run, 2026-10-17). It is now stored and never moves, as an amount's
-- position does not.
--
-- Each model keeps the position its rule has today, right after its
-- amounts, so nothing moves on the upgrade. A new model takes 0. The
-- trigger of quota_amounts gives a new amount the lowest free position
-- other than this one.
ALTER TABLE models ADD COLUMN budget_pass_slot integer NOT NULL DEFAULT 0 CHECK (budget_pass_slot >= 0);

UPDATE models m SET budget_pass_slot = coalesce((SELECT max(a.slot) + 1 FROM quota_amounts a WHERE a.model_id = m.id), 0);

CREATE OR REPLACE FUNCTION quota_amounts_sync() RETURNS trigger AS $$
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
        FROM generate_series(0, (SELECT count(*) FROM quota_amounts WHERE model_id = NEW.model_id)::int + 1) free
        WHERE NOT EXISTS (SELECT 1 FROM quota_amounts a WHERE a.model_id = NEW.model_id AND a.slot = free)
          AND free <> (SELECT budget_pass_slot FROM models WHERE id = NEW.model_id)
        ON CONFLICT ON CONSTRAINT quota_amounts_amount DO NOTHING;
    END IF;
    RETURN NULL;
END
$$ LANGUAGE plpgsql;
