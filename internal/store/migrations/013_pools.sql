-- The InferencePools that serve a model on a cluster, as read from the
-- cluster's LLMInferenceServices: [{"namespace": "...", "name": "...",
-- "group": "..."}]. The hub creates the request class "best-effort" for
-- each of them when the model is in best-effort mode.
ALTER TABLE model_endpoints ADD COLUMN pools jsonb NOT NULL DEFAULT '[]';
