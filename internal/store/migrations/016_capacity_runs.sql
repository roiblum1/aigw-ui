-- A measurement of what one instance of a model serves at the latency an
-- operator promises. AIPerf runs it in a Job on the cluster against one pod.
-- The Job makes up its own prompts; only the numbers reach the server.
CREATE TABLE capacity_runs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id    uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    cluster_id  uuid NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
    -- Where the Job runs, and the pod it measures.
    namespace   text NOT NULL,
    job_name    text NOT NULL,
    instance    text NOT NULL,
    -- The load steps, the latency targets and the cost of an instance.
    settings    jsonb NOT NULL,
    status      text NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),
    detail      text NOT NULL DEFAULT '',
    -- What the made-up sessions hold, and one entry per load step, added
    -- while the Job runs.
    dataset     jsonb,
    steps       jsonb NOT NULL DEFAULT '[]',
    created_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX capacity_runs_model ON capacity_runs (model_id, created_at DESC);
-- At most one measurement runs per model at a time: two would share the
-- GPUs of one site and both be wrong.
CREATE UNIQUE INDEX capacity_runs_one_running ON capacity_runs (model_id) WHERE status = 'running';

-- Ready instances of the model on the cluster, as last read. capacity_observed
-- is this times the declared capacity of one instance.
ALTER TABLE model_endpoints ADD COLUMN instances double precision;
